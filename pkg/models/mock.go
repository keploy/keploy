package models

import "time"

type Mock struct {
	Version Version  `json:"Version,omitempty"`
	Name    string   `json:"Name,omitempty"`
	Kind    Kind     `json:"Kind,omitempty"`
	Spec    MockSpec `json:"Spec,omitempty"`
	Id      string   `json:"Id,omitempty"`
}

func (m *Mock) GetKind() string {
	return string(m.Kind)
}

type MockSpec struct {
	Metadata map[string]string `json:"Metadata,omitempty" protobuf_key:"bytes,1,opt,name=key,proto3" protobuf_val:"bytes,2,opt,name=value,proto3"`
	// for GenericSpec
	GenericRequests  []GenericPayload `json:"RequestBin,omitempty"`
	GenericResponses []GenericPayload `json:"ResponseBin,omitempty"`
	// for HttpSpec
	HttpReq  *HttpReq  `json:"Req,omitempty"`
	HttpResp *HttpResp `json:"Res,omitempty"`
	Created  int64     `json:"Created,omitempty"`
	// for MongoSpec
	// MongoRequestHeader  *MongoHeader    `json:"RequestHeader,omitempty"`
	// MongoResponseHeader *MongoHeader    `json:"ResponseHeader,omitempty"`
	// MongoRequest        interface{}     `json:"MongoRequest,omitempty"`
	// MongoResponse       interface{}     `json:"MongoResponse,omitempty"`
	MongoRequests  []MongoRequest  `json:"MongoRequests,omitempty"`
	MongoResponses []MongoResponse `json:"MongoResponses,omitempty"`

	PostgresRequests  []Backend  `json:"postgresRequests,omitempty"`
	PostgresResponses []Frontend `json:"postgresResponses,omitempty"`

	//for grpc
	GRPCReq  *GrpcReq  `json:"gRPCRequest,omitempty"`
	GRPCResp *GrpcResp `json:"grpcResponse,omitempty"`
	//for MySql
	MySqlRequests  []MySQLRequest  `json:"MySqlRequests,omitempty"`
	MySqlResponses []MySQLResponse `json:"MySqlResponses,omitempty"`

	ReqTimestampMock time.Time `json:"ReqTimestampMock,omitempty"`
	ResTimestampMock time.Time `json:"ResTimestampMock,omitempty"`
}

// OutputBinary store the encoded binary output of the egress calls as base64-encoded strings
type OutputBinary struct {
	Type string `json:"type" yaml:"type"`
	Data string `json:"data" yaml:"data"`
}

type OriginType string

const (
	FromServer OriginType = "server"
	FromClient OriginType = "client"
)

type GenericPayload struct {
	Origin  OriginType     `json:"Origin,omitempty" yaml:"origin"`
	Message []OutputBinary `json:"Message,omitempty" yaml:"message"`
}
