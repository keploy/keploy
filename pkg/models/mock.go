package models

import (
	"encoding/gob"
	"time"

	"go.keploy.io/server/v3/pkg/models/mysql"
	"go.keploy.io/server/v3/pkg/models/postgres"
	"go.mongodb.org/mongo-driver/v2/bson"
)

func init() {
	gob.Register(bson.D{})
	gob.Register(bson.E{})
	gob.Register(bson.A{})
	gob.Register(bson.Binary{})
	gob.Register(bson.M{})
	gob.Register(bson.ObjectID{})
}

type Kind string

const (
	HTTP        Kind = "Http"
	HTTP2       Kind = "Http2"
	GENERIC     Kind = "Generic"
	REDIS       Kind = "Redis"
	MySQL       Kind = "MySQL"
	Postgres    Kind = "Postgres"
	PostgresV2  Kind = "PostgresV2"
	GRPC_EXPORT Kind = "gRPC"
	Mongo       Kind = "Mongo"
	DNS         Kind = "DNS"
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
	ID         int   `json:"Id,omitempty" bson:"Id,omitempty"`
	IsFiltered bool  `json:"isFiltered,omitempty" bson:"isFiltered,omitempty"`
	SortOrder  int64 `json:"sortOrder,omitempty" bson:"SortOrder,omitempty"`
}

func (m *Mock) GetKind() string {
	return string(m.Kind)
}

type MockSpec struct {
	Metadata            map[string]string   `json:"Metadata,omitempty" bson:"metadata,omitempty" protobuf_key:"bytes,1,opt,name=key,proto3" protobuf_val:"bytes,2,opt,name=value,proto3"`
	GenericRequests     []Payload           `json:"RequestBin,omitempty" bson:"generic_requests,omitempty"`
	GenericResponses    []Payload           `json:"ResponseBin,omitempty" bson:"generic_responses,omitempty"`
	RedisRequests       []Payload           `json:"redisRequests,omitempty" bson:"redis_requests,omitempty"`
	RedisResponses      []Payload           `json:"redisResponses,omitempty" bson:"redis_responses,omitempty"`
	HTTPReq             *HTTPReq            `json:"Req,omitempty" bson:"http_req,omitempty"`
	HTTPResp            *HTTPResp           `json:"Res,omitempty" bson:"http_resp,omitempty"`
	Created             int64               `json:"Created,omitempty" bson:"created,omitempty"`
	MongoRequests       []MongoRequest      `json:"MongoRequests,omitempty" bson:"mongo_requests,omitempty"`
	MongoResponses      []MongoResponse     `json:"MongoResponses,omitempty" bson:"mongo_responses,omitempty"`
	PostgresRequestsV2  []postgres.Request  `json:"PostgresRequestsV2,omitempty" bson:"postgres_requests_v2,omitempty"`
	PostgresResponsesV2 []postgres.Response `json:"PostgresResponsesV2,omitempty" bson:"postgres_responses_v2,omitempty"`
	// gRPC
	GRPCReq        *GrpcReq         `json:"gRPCRequest,omitempty" bson:"grpc_req,omitempty"`
	GRPCResp       *GrpcResp        `json:"grpcResponse,omitempty" bson:"grpc_resp,omitempty"`
	MySQLRequests  []mysql.Request  `json:"MySqlRequests,omitempty" bson:"my_sql_requests,omitempty"`
	MySQLResponses []mysql.Response `json:"MySqlResponses,omitempty" bson:"my_sql_responses,omitempty"`
	DNSReq         *DNSReq          `json:"dnsReq,omitempty" bson:"dns_req,omitempty"`
	DNSResp        *DNSResp         `json:"dnsResp,omitempty" bson:"dns_resp,omitempty"`
	// HTTP/2
	HTTP2Req         *HTTP2Req  `json:"http2Req,omitempty" bson:"http2_req,omitempty"`
	HTTP2Resp        *HTTP2Resp `json:"http2Resp,omitempty" bson:"http2_resp,omitempty"`
	ReqTimestampMock time.Time  `json:"ReqTimestampMock,omitempty" bson:"req_timestamp_mock,omitempty"`
	ResTimestampMock time.Time  `json:"ResTimestampMock,omitempty" bson:"res_timestamp_mock,omitempty"`
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

type MockUsage string

const (
	Updated MockUsage = "updated"
	Deleted MockUsage = "deleted"
)

type Payload struct {
	Origin  OriginType     `json:"Origin,omitempty" yaml:"origin" bson:"origin,omitempty"`
	Message []OutputBinary `json:"Message,omitempty" yaml:"message" bson:"message,omitempty"`
}

type MockState struct {
	Name       string    `json:"name"`
	Usage      MockUsage `json:"usage"`
	IsFiltered bool      `json:"isFiltered"`
	SortOrder  int64     `json:"sortOrder"`
}

func (m *Mock) DeepCopy() *Mock {
	if m == nil {
		return nil
	}

	// Copy top-level fields explicitly to avoid copying embedded lock fields.
	id, isFiltered, sortOrder := m.TestModeInfo.ID, m.TestModeInfo.IsFiltered, m.TestModeInfo.SortOrder
	c := Mock{
		Version: m.Version,
		Name:    m.Name,
		Kind:    m.Kind,
		Spec:    m.Spec,
		TestModeInfo: TestModeInfo{
			ID:         id,
			IsFiltered: isFiltered,
			SortOrder:  sortOrder,
		},
		ConnectionID: m.ConnectionID,
	}

	// 2. Deep copy the map by creating a new one and copying key-value pairs.
	if m.Spec.Metadata != nil {
		c.Spec.Metadata = make(map[string]string, len(m.Spec.Metadata))
		for k, v := range m.Spec.Metadata {
			c.Spec.Metadata[k] = v
		}
	}

	// 3. Deep copy all slices by creating new slices and copying the elements.
	// This gives each copy its own separate backing array.
	c.Spec.GenericRequests = make([]Payload, len(m.Spec.GenericRequests))
	copy(c.Spec.GenericRequests, m.Spec.GenericRequests)

	c.Spec.GenericResponses = make([]Payload, len(m.Spec.GenericResponses))
	copy(c.Spec.GenericResponses, m.Spec.GenericResponses)

	c.Spec.RedisRequests = make([]Payload, len(m.Spec.RedisRequests))
	copy(c.Spec.RedisRequests, m.Spec.RedisRequests)

	c.Spec.RedisResponses = make([]Payload, len(m.Spec.RedisResponses))
	copy(c.Spec.RedisResponses, m.Spec.RedisResponses)

	c.Spec.MongoRequests = make([]MongoRequest, len(m.Spec.MongoRequests))
	copy(c.Spec.MongoRequests, m.Spec.MongoRequests)

	c.Spec.MongoResponses = make([]MongoResponse, len(m.Spec.MongoResponses))
	copy(c.Spec.MongoResponses, m.Spec.MongoResponses)

	c.Spec.MySQLRequests = make([]mysql.Request, len(m.Spec.MySQLRequests))
	copy(c.Spec.MySQLRequests, m.Spec.MySQLRequests)

	c.Spec.MySQLResponses = make([]mysql.Response, len(m.Spec.MySQLResponses))
	copy(c.Spec.MySQLResponses, m.Spec.MySQLResponses)

	// 4. Deep copy all pointers by creating a new object and copying the value.
	if m.Spec.HTTPReq != nil {
		httpReqCopy := *m.Spec.HTTPReq
		c.Spec.HTTPReq = &httpReqCopy
	}
	if m.Spec.HTTPResp != nil {
		httpRespCopy := *m.Spec.HTTPResp
		c.Spec.HTTPResp = &httpRespCopy
	}
	if m.Spec.GRPCReq != nil {
		grpcReqCopy := *m.Spec.GRPCReq
		c.Spec.GRPCReq = &grpcReqCopy
	}
	if m.Spec.GRPCResp != nil {
		grpcRespCopy := *m.Spec.GRPCResp
		c.Spec.GRPCResp = &grpcRespCopy
	}
	if m.Spec.DNSReq != nil {
		dnsReqCopy := *m.Spec.DNSReq
		c.Spec.DNSReq = &dnsReqCopy
	}
	if m.Spec.DNSResp != nil {
		dnsRespCopy := *m.Spec.DNSResp
		if m.Spec.DNSResp.Answers != nil {
			dnsRespCopy.Answers = make([]string, len(m.Spec.DNSResp.Answers))
			copy(dnsRespCopy.Answers, m.Spec.DNSResp.Answers)
		}
		c.Spec.DNSResp = &dnsRespCopy
	}

	if m.Spec.HTTP2Req != nil {
		http2ReqCopy := *m.Spec.HTTP2Req
		if m.Spec.HTTP2Req.Headers != nil {
			http2ReqCopy.Headers = make(map[string]string, len(m.Spec.HTTP2Req.Headers))
			for k, v := range m.Spec.HTTP2Req.Headers {
				http2ReqCopy.Headers[k] = v
			}
		}
		c.Spec.HTTP2Req = &http2ReqCopy
	}
	if m.Spec.HTTP2Resp != nil {
		http2RespCopy := *m.Spec.HTTP2Resp
		if m.Spec.HTTP2Resp.Headers != nil {
			http2RespCopy.Headers = make(map[string]string, len(m.Spec.HTTP2Resp.Headers))
			for k, v := range m.Spec.HTTP2Resp.Headers {
				http2RespCopy.Headers[k] = v
			}
		}
		if m.Spec.HTTP2Resp.Trailers != nil {
			http2RespCopy.Trailers = make(map[string]string, len(m.Spec.HTTP2Resp.Trailers))
			for k, v := range m.Spec.HTTP2Resp.Trailers {
				http2RespCopy.Trailers[k] = v
			}
		}
		c.Spec.HTTP2Resp = &http2RespCopy
	}

	return &c
}

type ReRecordCfg struct {
	Rerecord bool
	TestSet  string
}
