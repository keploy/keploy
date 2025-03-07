package models

import (
	"time"

	"go.keploy.io/server/v2/pkg/models/mysql"
)

type Mock struct {
	Version      Version      `json:"Version,omitempty" bson:"Version,omitempty"`
	Name         string       `json:"Name,omitempty" bson:"Name,omitempty"`
	Kind         Kind         `json:"Kind,omitempty" bson:"Kind,omitempty"`
	Spec         MockSpec     `json:"Spec,omitempty" bson:"Spec,omitempty"`
	TestModeInfo TestModeInfo `json:"TestModeInfo,omitempty"  bson:"TestModeInfo,omitempty"` // Map for additional test mode information
	ConnectionID string       `json:"ConnectionId,omitempty" bson:"ConnectionId,omitempty"`
}

type TestModeInfo struct {
	ID         int  `json:"Id,omitempty" bson:"Id,omitempty"`
	IsFiltered bool `json:"isFiltered,omitempty" bson:"isFiltered,omitempty"`
	SortOrder  int  `json:"sortOrder,omitempty" bson:"SortOrder,omitempty"`
}

func (m *Mock) GetKind() string {
	return string(m.Kind)
}

type MockSpec struct {
	Metadata          map[string]string `json:"Metadata,omitempty" bson:"metadata,omitempty" protobuf_key:"bytes,1,opt,name=key,proto3" protobuf_val:"bytes,2,opt,name=value,proto3"`
	GenericRequests   []Payload         `json:"RequestBin,omitempty" bson:"generic_requests,omitempty"`
	GenericResponses  []Payload         `json:"ResponseBin,omitempty" bson:"generic_responses,omitempty"`
	RedisRequests     []Payload         `json:"redisRequests,omitempty" bson:"redis_requests,omitempty"`
	RedisResponses    []Payload         `json:"redisResponses,omitempty" bson:"redis_responses,omitempty"`
	HTTPReq           *HTTPReq          `json:"Req,omitempty" bson:"http_req,omitempty"`
	HTTPResp          *HTTPResp         `json:"Res,omitempty" bson:"http_resp,omitempty"`
	XMLResp           *XMLResp          `json:"XMLResp,omitempty" bson:"xml_resp,omitempty"`
	Created           int64             `json:"Created,omitempty" bson:"created,omitempty"`
	MongoRequests     []MongoRequest    `json:"MongoRequests,omitempty" bson:"mongo_requests,omitempty"`
	MongoResponses    []MongoResponse   `json:"MongoResponses,omitempty" bson:"mongo_responses,omitempty"`
	PostgresRequests  []Backend         `json:"postgresRequests,omitempty" bson:"postgres_requests,omitempty"`
	PostgresResponses []Frontend        `json:"postgresResponses,omitempty" bson:"postgres_responses,omitempty"`
	GRPCReq           *GrpcReq          `json:"gRPCRequest,omitempty" bson:"grpc_req,omitempty"`
	GRPCResp          *GrpcResp         `json:"grpcResponse,omitempty" bson:"grpc_resp,omitempty"`
	MySQLRequests     []mysql.Request   `json:"MySqlRequests,omitempty" bson:"my_sql_requests,omitempty"`
	MySQLResponses    []mysql.Response  `json:"MySqlResponses,omitempty" bson:"my_sql_responses,omitempty"`
	ReqTimestampMock  time.Time         `json:"ReqTimestampMock,omitempty" bson:"req_timestamp_mock,omitempty"`
	ResTimestampMock  time.Time         `json:"ResTimestampMock,omitempty" bson:"res_timestamp_mock,omitempty"`
}

// OutputBinary store the encoded binary output of the egress calls as base64-encoded strings
type OutputBinary struct {
	Type string `json:"type" bson:"type" yaml:"type"`
	Data string `json:"data" bson:"data" yaml:"data"`
}

type OriginType string

// constant for mock origin
const (
	FromServer OriginType = "server"
	FromClient OriginType = "client"
)

type Payload struct {
	Origin  OriginType     `json:"Origin,omitempty" yaml:"origin" bson:"origin,omitempty"`
	Message []OutputBinary `json:"Message,omitempty" yaml:"message" bson:"message,omitempty"`
}
