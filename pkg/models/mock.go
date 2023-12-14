package models

import "time"

type Mock struct {
	Version Version  `json:"Version,omitempty" bson:"Version,omitempty"`
	Name    string   `json:"Name,omitempty" bson:"Name,omitempty"`
	Kind    Kind     `json:"Kind,omitempty" bson:"Kind,omitempty"`
	Spec    MockSpec `json:"Spec,omitempty" bson:"Spec,omitempty"`
}

func (m *Mock) GetKind() string {
	return string(m.Kind)
}

type MockSpec struct {
	Metadata          map[string]string `json:"Metadata,omitempty" bson:"Metadata,omitempty" protobuf_key:"bytes,1,opt,name=key,proto3" protobuf_val:"bytes,2,opt,name=value,proto3"`
	GenericRequests   []GenericPayload  `json:"RequestBin,omitempty" bson:"RequestBin,omitempty"`
	GenericResponses  []GenericPayload  `json:"ResponseBin,omitempty" bson:"ResponseBin,omitempty"`
	HttpReq           *HttpReq          `json:"Req,omitempty" bson:"Req,omitempty"`
	HttpResp          *HttpResp         `json:"Res,omitempty" bson:"Res,omitempty"`
	Created           int64             `json:"Created,omitempty" bson:"Created,omitempty"`
	MongoRequests     []MongoRequest    `json:"MongoRequests,omitempty" bson:"MongoRequests,omitempty"`
	MongoResponses    []MongoResponse   `json:"MongoResponses,omitempty" bson:"MongoResponses,omitempty"`
	PostgresRequests  []Backend         `json:"postgresRequests,omitempty" bson:"postgresRequests,omitempty"`
	PostgresResponses []Frontend        `json:"postgresResponses,omitempty" bson:"postgresResponses,omitempty"`
	GRPCReq           *GrpcReq          `json:"gRPCRequest,omitempty" bson:"gRPCRequest,omitempty"`
	GRPCResp          *GrpcResp         `json:"grpcResponse,omitempty" bson:"grpcResponse,omitempty"`
	MySqlRequests     []MySQLRequest    `json:"MySqlRequests,omitempty" bson:"MySqlRequests,omitempty"`
	MySqlResponses    []MySQLResponse   `json:"MySqlResponses,omitempty" bson:"MySqlResponses,omitempty"`
	ReqTimestampMock  time.Time         `json:"ReqTimestampMock,omitempty" bson:"ReqTimestampMock,omitempty"`
	ResTimestampMock  time.Time         `json:"ResTimestampMock,omitempty" bson:"ResTimestampMock,omitempty"`
}

// OutputBinary store the encoded binary output of the egress calls as base64-encoded strings
type OutputBinary struct {
	Type string `json:"type" bson:"type" yaml:"type"`
	Data string `json:"data" bson:"data" yaml:"data"`
}

type OriginType string

const (
	FromServer OriginType = "server"
	FromClient OriginType = "client"
)

type GenericPayload struct {
	Origin  OriginType     `json:"Origin,omitempty" bson:"Origin,omitempty" yaml:"origin"`
	Message []OutputBinary `json:"Message,omitempty" bson:"Message,omitempty" yaml:"message"`
}
