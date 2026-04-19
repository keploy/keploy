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
	KAFKA       Kind = "Kafka"
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
	// Noise holds exact-match regex patterns for obfuscated values.
	// During mock matching, any stored value matching a pattern in this
	// list is skipped (treated as noise). Written by the enterprise
	// secret-protection obfuscator.
	Noise []string `json:"Noise,omitempty" bson:"noise,omitempty" yaml:"noise,omitempty"`
	// Format is the per-mock on-disk format hint/override. Empty string
	// means "fall back to the testset-level format" (whatever the caller
	// / CLI selected via record.mockFormat / KEPLOY_MOCK_FORMAT).
	// Recognized values are "yaml" or "gob"; anything else is treated as
	// empty and falls back to the process-wide configured default (see
	// mockdb.resolveMockFormat). We deliberately do not reject unknown
	// values so a stale or typo'd format never blocks recording.
	//
	// Writers may propagate this field into the on-disk
	// NetworkTrafficDoc, and readers may load it back from the same
	// field. However, this metadata alone does not imply that mocks from
	// both mocks.yaml and mocks.gob are merged when both files exist in a
	// single test-set directory. No default: an unset Format means "use
	// whatever mockdb was configured with at startup".
	Format string `json:"Format,omitempty" bson:"format,omitempty" yaml:"format,omitempty"`
}

// TestModeInfo is in-memory-only bookkeeping attached to each Mock once it
// enters a runtime pool (disk load, live recorder, agent StoreMocks).
// None of these fields are serialised to the YAML wire format — the
// platform/yaml NetworkTrafficDoc does not embed TestModeInfo — so this
// struct is the right home for cached derived state that must not bleed
// into recordings.
//
// Lifetime and HitCount were added by the unification plan: Lifetime is
// the typed, cached form of the on-disk Spec.Metadata["type"] tag so
// hot-path matchers never probe the metadata map; HitCount is an atomic
// reuse counter used for telemetry of session/connection-scoped mocks
// (how many times was this reusable mock actually matched across the
// test run).
type TestModeInfo struct {
	ID         int   `json:"Id,omitempty" bson:"Id,omitempty"`
	IsFiltered bool  `json:"isFiltered,omitempty" bson:"isFiltered,omitempty"`
	SortOrder  int64 `json:"sortOrder,omitempty" bson:"SortOrder,omitempty"`

	// Lifetime classifies the mock (per-test / session / connection)
	// once at ingest via (*Mock).DeriveLifetime. Matchers read this
	// field directly — do NOT re-probe Spec.Metadata["type"] in hot
	// paths. The field is intentionally untagged for JSON/BSON/YAML:
	// it is derived from on-disk state on every load, so persisting it
	// would create a second source of truth.
	Lifetime Lifetime `json:"-" bson:"-"`

	// LifetimeDerived is set to true the first time DeriveLifetime
	// completes on this mock. Without this flag, DeriveLifetime cannot
	// distinguish "never derived" from "derived to LifetimePerTest"
	// (they share the zero value) and would re-run on every call for
	// per-test mocks — a minor but avoidable cost given DeriveLifetime
	// runs at every ingest site (disk load, StoreMocks, syncMock).
	// Runtime-only, untagged; re-derived fresh on each reload.
	LifetimeDerived bool `json:"-" bson:"-"`

	// HitCount is incremented atomically on every successful match of
	// session- or connection-scoped mocks (per-test mocks are consumed
	// on match so their count is always 0 or 1). Zero-cost when idle
	// (single LOCK XADD on x86, ~1 ns). Surfaced via MockMemDb's
	// SessionMockHitCounts for "which reusable mocks actually got
	// used?" observability — non-zero helps confirm tagging; zero for
	// a long-lived mock hints at dead recordings worth re-capturing.
	HitCount uint64 `json:"-" bson:"-"`
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
	KafkaRequests       []Payload           `json:"kafkaRequests,omitempty" bson:"kafka_requests,omitempty"`
	KafkaResponses      []Payload           `json:"kafkaResponses,omitempty" bson:"kafka_responses,omitempty"`
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
	Name             string    `json:"name"`
	Kind             Kind      `json:"kind"`
	Usage            MockUsage `json:"usage"`
	IsFiltered       bool      `json:"isFiltered"`
	SortOrder        int64     `json:"sortOrder"`
	Type             string    `json:"type"`
	Timestamp        int64     `json:"timestamp"`
	ReqTimestampMock string    `json:"reqTimestampMock,omitempty"`
	ResTimestampMock string    `json:"resTimestampMock,omitempty"`
}

func (m *Mock) DeepCopy() *Mock {
	if m == nil {
		return nil
	}

	// Copy top-level fields explicitly to avoid copying embedded lock fields.
	// HitCount is intentionally NOT carried over: the counter is bound to
	// the live mock pool instance (it tracks matches against *this*
	// agent's in-memory pool), so a deep copy starts with a fresh counter.
	// Callers who want cumulative counts across copies should aggregate at
	// the MockMemDb level, not via clones. Lifetime + LifetimeDerived ARE
	// carried over — they're classification state, not runtime counters;
	// skipping LifetimeDerived would cause DeriveLifetime to re-run on
	// the copy and double-increment LegacyKindFallbackFires for untagged
	// kinds.
	id := m.TestModeInfo.ID
	isFiltered := m.TestModeInfo.IsFiltered
	sortOrder := m.TestModeInfo.SortOrder
	lifetime := m.TestModeInfo.Lifetime
	lifetimeDerived := m.TestModeInfo.LifetimeDerived
	c := Mock{
		Version: m.Version,
		Name:    m.Name,
		Kind:    m.Kind,
		Spec:    m.Spec,
		TestModeInfo: TestModeInfo{
			ID:              id,
			IsFiltered:      isFiltered,
			SortOrder:       sortOrder,
			Lifetime:        lifetime,
			LifetimeDerived: lifetimeDerived,
		},
		ConnectionID: m.ConnectionID,
		// Format is a per-mock override; it must survive DeepCopy so the
		// async gob writer's copied payload preserves the caller's
		// selected on-disk format. Dropping it here would silently demote
		// gob-tagged mocks to the default format on the write path.
		Format: m.Format,
	}

	// Deep copy the Noise slice so mutations to one copy don't affect the other.
	if len(m.Noise) > 0 {
		c.Noise = make([]string, len(m.Noise))
		copy(c.Noise, m.Noise)
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
