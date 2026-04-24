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
	HTTP       Kind = "Http"
	HTTP2      Kind = "Http2"
	GENERIC    Kind = "Generic"
	REDIS      Kind = "Redis"
	KAFKA      Kind = "Kafka"
	MySQL      Kind = "MySQL"
	Postgres   Kind = "Postgres"
	PostgresV2 Kind = "PostgresV2"

	// PostgresV3 is the single top-level Kind for the v3 Postgres parser.
	// The sub-type (session / catalog / data / query / generator) lives in
	// Spec.PostgresV3.Type; consumers discriminate there instead of on Kind.
	// See integrations/pkg/postgres/v3/README.md for the replay architecture.
	PostgresV3 Kind = "PostgresV3"

	GRPC_EXPORT Kind = "gRPC"
	Mongo       Kind = "Mongo"
	DNS         Kind = "DNS"
)

// MockName constants for the PostgresV3 parser. The integrations-side
// recorder currently hardcodes these values as string literals when
// stamping Mock.Name; exposing them on the keploy side lets a future
// integrations-repo commit migrate to the shared constants without
// drifting the spelling (a typo in the recorder would silently split
// the pool into two effectively unrelated subsets).
//
// Mock.Name values are identifiers, not display text — they participate
// in hit-count indexing, dedup, and by-name lookups in MockManager.
// Keep these exact strings stable across releases; if the spelling ever
// needs to change it must be coordinated with the integrations repo and
// with any recorded YAML artefacts that reference the old names.
const (
	MockNamePostgresV3Query   = "PostgresV3Query"
	MockNamePostgresV3Session = "PostgresV3Session"
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

	// PostgresV3 is the single discriminated spec for the v3 Postgres parser.
	// Exactly one sub-pointer is populated; Type names which. See PostgresV3Spec.
	PostgresV3 *PostgresV3Spec `yaml:"postgresV3,omitempty" json:"postgresV3,omitempty" bson:"postgres_v3,omitempty"`
}

// PostgresV3Spec is the single discriminated Spec for the five v3
// mock sub-types. Exactly one of the pointer fields is non-nil, and
// Type names which. A nil PostgresV3Spec or a Type that doesn't match
// the populated pointer is a hard-reject at BuildIndex time.
type PostgresV3Spec struct {
	Type string `yaml:"type" json:"type" bson:"type"` // "session" | "catalog" | "data" | "query" | "generator"

	Session   *PostgresV3SessionSpec   `yaml:"session,omitempty"   json:"session,omitempty"   bson:"session,omitempty"`
	Catalog   *PostgresV3CatalogSpec   `yaml:"catalog,omitempty"   json:"catalog,omitempty"   bson:"catalog,omitempty"`
	Data      *PostgresV3DataSpec      `yaml:"data,omitempty"      json:"data,omitempty"      bson:"data,omitempty"`
	Query     *PostgresV3QuerySpec     `yaml:"query,omitempty"     json:"query,omitempty"     bson:"query,omitempty"`
	Generator *PostgresV3GeneratorSpec `yaml:"generator,omitempty" json:"generator,omitempty" bson:"generator,omitempty"`
}

// PostgresV3 sub-type discriminator values (Spec.PostgresV3.Type).
const (
	PostgresV3TypeSession   = "session"
	PostgresV3TypeCatalog   = "catalog"
	PostgresV3TypeData      = "data"
	PostgresV3TypeQuery     = "query"
	PostgresV3TypeGenerator = "generator"
)

// ============================================================================
// PostgresV3 specs — deterministic replay payloads.
//
// See integrations/pkg/postgres/v3/types/contracts.go for the in-memory
// type hierarchy these structs serialize from.
// ============================================================================

// PostgresV3SessionSpec — startup handshake + ParameterStatus bundle.
// Exactly one per recording; emitted on every replay client connection
// after a trust-mode AuthOk.
type PostgresV3SessionSpec struct {
	ProtocolVersion  string            `json:"protocolVersion,omitempty" yaml:"protocolVersion,omitempty" bson:"protocol_version,omitempty"`
	SSLResponse      string            `json:"sslResponse,omitempty" yaml:"sslResponse,omitempty" bson:"ssl_response,omitempty"`
	ServerVersion    string            `json:"serverVersion,omitempty" yaml:"serverVersion,omitempty" bson:"server_version,omitempty"`
	ParameterStatus  map[string]string `json:"parameterStatus,omitempty" yaml:"parameterStatus,omitempty" bson:"parameter_status,omitempty"`
	BackendProcessID int32             `json:"backendProcessID,omitempty" yaml:"backendProcessID,omitempty" bson:"backend_process_id,omitempty"`
	BackendSecretKey int32             `json:"backendSecretKey,omitempty" yaml:"backendSecretKey,omitempty" bson:"backend_secret_key,omitempty"`
	ObservedAuthMode string            `json:"observedAuthMode,omitempty" yaml:"observedAuthMode,omitempty" bson:"observed_auth_mode,omitempty"`
}

// PostgresV3CatalogSpec — structured pg_catalog + information_schema
// snapshot consulted by the replayer's L5 catalog engine for ORM
// metadata probes.
type PostgresV3CatalogSpec struct {
	Schemas        []PostgresV3Schema         `json:"schemas,omitempty" yaml:"schemas,omitempty" bson:"schemas,omitempty"`
	Types          []PostgresV3PgType         `json:"types,omitempty" yaml:"types,omitempty" bson:"types,omitempty"`
	Sequences      []PostgresV3Sequence       `json:"sequences,omitempty" yaml:"sequences,omitempty" bson:"sequences,omitempty"`
	Extensions     []string                   `json:"extensions,omitempty" yaml:"extensions,omitempty" bson:"extensions,omitempty"`
	MigrationState []PostgresV3MigrationTable `json:"migrationState,omitempty" yaml:"migrationState,omitempty" bson:"migration_state,omitempty"`
}

type PostgresV3Schema struct {
	Name   string               `json:"name" yaml:"name" bson:"name"`
	Tables []PostgresV3TableDef `json:"tables,omitempty" yaml:"tables,omitempty" bson:"tables,omitempty"`
}

type PostgresV3TableDef struct {
	Name        string                 `json:"name" yaml:"name" bson:"name"`
	Columns     []PostgresV3Column     `json:"columns,omitempty" yaml:"columns,omitempty" bson:"columns,omitempty"`
	Indexes     []PostgresV3IndexDef   `json:"indexes,omitempty" yaml:"indexes,omitempty" bson:"indexes,omitempty"`
	Constraints []PostgresV3Constraint `json:"constraints,omitempty" yaml:"constraints,omitempty" bson:"constraints,omitempty"`
}

type PostgresV3Column struct {
	Name      string `json:"name" yaml:"name" bson:"name"`
	TypeOID   uint32 `json:"typeOid" yaml:"typeOid" bson:"type_oid"`
	TypeName  string `json:"typeName" yaml:"typeName" bson:"type_name"`
	NotNull   bool   `json:"notNull,omitempty" yaml:"notNull,omitempty" bson:"not_null,omitempty"`
	Default   string `json:"default,omitempty" yaml:"default,omitempty" bson:"default,omitempty"`
	IsPrimary bool   `json:"isPrimary,omitempty" yaml:"isPrimary,omitempty" bson:"is_primary,omitempty"`
	AttNum    int16  `json:"attNum" yaml:"attNum" bson:"att_num"`
}

type PostgresV3IndexDef struct {
	Name    string   `json:"name" yaml:"name" bson:"name"`
	Columns []string `json:"columns" yaml:"columns" bson:"columns"`
	Unique  bool     `json:"unique,omitempty" yaml:"unique,omitempty" bson:"unique,omitempty"`
}

type PostgresV3Constraint struct {
	Name    string   `json:"name" yaml:"name" bson:"name"`
	Type    string   `json:"type" yaml:"type" bson:"type"`
	Columns []string `json:"columns" yaml:"columns" bson:"columns"`
}

type PostgresV3PgType struct {
	Name string `json:"name" yaml:"name" bson:"name"`
	OID  uint32 `json:"oid" yaml:"oid" bson:"oid"`
	Size int16  `json:"size" yaml:"size" bson:"size"`
	Kind string `json:"kind" yaml:"kind" bson:"kind"`
}

type PostgresV3Sequence struct {
	Schema    string `json:"schema" yaml:"schema" bson:"schema"`
	Name      string `json:"name" yaml:"name" bson:"name"`
	Start     int64  `json:"start" yaml:"start" bson:"start"`
	Increment int64  `json:"increment" yaml:"increment" bson:"increment"`
	LastValue int64  `json:"lastValue" yaml:"lastValue" bson:"last_value"`
}

type PostgresV3MigrationTable struct {
	Name    string            `json:"name" yaml:"name" bson:"name"`
	Columns []string          `json:"columns" yaml:"columns" bson:"columns"`
	Rows    []PostgresV3Cells `json:"rows,omitempty" yaml:"rows,omitempty" bson:"rows,omitempty"`
}

// PostgresV3DataSpec — one per seeded user table. Carries the row-store
// seed for L4's transactional engine.
type PostgresV3DataSpec struct {
	Schema     string            `json:"schema" yaml:"schema" bson:"schema"`
	Table      string            `json:"table" yaml:"table" bson:"table"`
	PrimaryKey []string          `json:"primaryKey,omitempty" yaml:"primaryKey,omitempty" bson:"primary_key,omitempty"`
	Columns    []string          `json:"columns" yaml:"columns" bson:"columns"`
	Rows       []PostgresV3Cells `json:"rows,omitempty" yaml:"rows,omitempty" bson:"rows,omitempty"`
	Truncated  bool              `json:"truncated,omitempty" yaml:"truncated,omitempty" bson:"truncated,omitempty"`
	RowLimit   int               `json:"rowLimit,omitempty" yaml:"rowLimit,omitempty" bson:"row_limit,omitempty"`
}

// PostgresV3QuerySpec — one invocation of a recorded query, keyed in the
// replay-time index by sqlAstHash.
//
// Historical note: earlier recordings stamped a `scope` field
// ("connection" / "session" / "test:<name>") alongside Class / Lifetime.
// The field was retired after 28069e28 moved pool routing to
// lifetime-first and pgmatch.DeriveLifetime became the single source
// of truth. Old YAML mocks that still contain `scope: ...` continue to
// load cleanly: gopkg.in/yaml.v3 is non-strict by default, so the
// unknown key is silently skipped at unmarshal time. NEW recordings
// MUST NOT re-introduce a scope field.
type PostgresV3QuerySpec struct {
	// Classification
	Class      string `json:"class,omitempty" yaml:"class,omitempty" bson:"class,omitempty"`
	Lifetime   string `json:"lifetime,omitempty" yaml:"lifetime,omitempty" bson:"lifetime,omitempty"`
	SQLAstHash string `json:"sqlAstHash" yaml:"sqlAstHash" bson:"sql_ast_hash"`

	// SQL
	SQLNormalized     string   `json:"sqlNormalized" yaml:"sqlNormalized" bson:"sql_normalized"`
	Relations         []string `json:"relations,omitempty" yaml:"relations,omitempty" bson:"relations,omitempty"`
	ParamOIDs         []uint32 `json:"paramOids,omitempty" yaml:"paramOids,omitempty" bson:"param_oids,omitempty"`
	VolatilePositions []int    `json:"volatilePositions,omitempty" yaml:"volatilePositions,omitempty" bson:"volatile_positions,omitempty"`

	// InvocationID uniquely identifies one invocation of this query
	// within a single record-session. Recorders should emit the
	// canonical "<connID>:<seq>" form — e.g. "0:1968" for the 1968th
	// invocation on connection 0. No hash prefix, no wall-clock
	// timestamp: a repeat recording of identical traffic will produce
	// identical InvocationIDs, which keeps diffs of re-recorded
	// mocks.yaml quiet. The field is stored as a plain string and is
	// not validated, so older or non-canonical values (e.g. legacy
	// "sha256:..." forms) still load cleanly for backward compatibility.
	// Uniqueness is local to one recording; the <sqlAstHash,
	// invocationId> pair is globally unique across all mocks in the
	// same file.
	InvocationID     string `json:"invocationId" yaml:"invocationId" bson:"invocation_id"`
	PrecedingTxState string `json:"precedingTxState,omitempty" yaml:"precedingTxState,omitempty" bson:"preceding_tx_state,omitempty"`

	// BindValues holds the client-supplied bind parameters for this
	// invocation, one entry per placeholder. Cells are stored as
	// logical Go values (int64, float64, string, bool, time.Time, []byte,
	// or PostgresV3CellRaw for unknown OIDs) irrespective of the wire
	// format the client used; PostgresV3Cell.MarshalYAML picks the
	// appropriate scalar tag per value so common types stay eyeballable
	// and greppable in mocks.yaml while bytea and raw-OID payloads land
	// as !!binary. NULL binds are distinguished from empty-string binds
	// via Cell.IsNull. BindFormats records the client's per-placeholder
	// format flag (0=text, 1=binary) so the replayer can re-encode the
	// logical value on the wire in the form the live client expects,
	// regardless of which form the recorder originally captured.
	BindValues  PostgresV3Cells `json:"bindValues,omitempty" yaml:"bindValues,omitempty" bson:"bind_values,omitempty"`
	BindFormats []int           `json:"bindFormats,omitempty" yaml:"bindFormats,omitempty" bson:"bind_formats,omitempty"`

	// ResultFormats — per-column format codes the client requested at
	// Bind time (via the Bind packet's ResultFormatCodes field).
	// Semantics match PG wire: len=0 means "all text", len=1 means
	// "broadcast f[0] to every result column", len=N means per-column.
	// Required at replay so the dispatcher can synthesise a
	// RowDescription with correct Format fields when the client did
	// not issue Describe before Execute (INSERT…RETURNING-style flow
	// under lib/pq / database/sql). Without this field, the replayer
	// has no way to know whether the recorded DataRow bytes are text
	// or binary, and drivers that expect binary int4 will fail text-
	// parsing on "\x00\x00\x00\x01" with "invalid syntax".
	ResultFormats []int `json:"resultFormats,omitempty" yaml:"resultFormats,omitempty" bson:"result_formats,omitempty"`

	// Wire response
	Response *PostgresV3Response `json:"response,omitempty" yaml:"response,omitempty" bson:"response,omitempty"`

	// State effects
	SideEffects *PostgresV3SideEffects `json:"sideEffects,omitempty" yaml:"sideEffects,omitempty" bson:"side_effects,omitempty"`
}

type PostgresV3Response struct {
	RowDescription []PostgresV3ColumnDescriptor `json:"rowDescription,omitempty" yaml:"rowDescription,omitempty" bson:"row_description,omitempty"`
	// Rows stores each row as a []PostgresV3Cell via PostgresV3Cells.
	// See PostgresV3Cell for the on-disk encoding rules (plain YAML
	// string for UTF-8-safe text, !!binary for anything else, explicit
	// native YAML null for SQL NULL). Mutually exclusive with CopyOut /
	// CopyIn — a single Query response emits either DataRow traffic
	// OR CopyData traffic, never both; the replay emitter enforces
	// the invariant at wire time.
	Rows            []PostgresV3Cells `json:"rows,omitempty" yaml:"rows,omitempty" bson:"rows,omitempty"`
	CommandComplete string            `json:"commandComplete,omitempty" yaml:"commandComplete,omitempty" bson:"command_complete,omitempty"`
	Error           *PostgresV3Error  `json:"error,omitempty" yaml:"error,omitempty" bson:"error,omitempty"`

	// CopyOut is set when the server responded with a CopyOutResponse
	// ('H') + zero or more CopyData ('d') chunks + CopyDone ('c')
	// sequence, as produced by COPY ... TO STDOUT / TO PROGRAM /
	// TO '<file>' queries. Zero CopyData chunks is valid — COPY of an
	// empty table or an empty-result expression produces no data
	// packets between CopyOutResponse and CopyDone. nil on non-COPY
	// responses; omitempty keeps the serialised form clean for them.
	CopyOut *PostgresV3CopyOutPayload `json:"copyOut,omitempty" yaml:"copyOut,omitempty" bson:"copy_out,omitempty"`

	// CopyIn is set when the server responded with a CopyInResponse
	// ('G'), signalling that it is ready to receive client-streamed
	// data via CopyData/CopyDone (COPY ... FROM STDIN). Only the
	// server-side "ready" metadata is persisted — the client's
	// subsequent CopyData payload is not replayed back, since the
	// replay side stands in as the server and the real client will
	// resend its own bytes. nil on non-COPY responses.
	CopyIn *PostgresV3CopyInPayload `json:"copyIn,omitempty" yaml:"copyIn,omitempty" bson:"copy_in,omitempty"`

	// Notices holds NoticeResponse ('N') messages the server emitted
	// while answering this invocation. Postgres interleaves notices
	// with any command response (e.g. "NOTICE: relation already
	// exists, skipping" after CREATE TABLE IF NOT EXISTS). ORMs and
	// drivers that surface NOTICE/WARNING to the caller depend on
	// these being replayed back, so we persist them rather than
	// silently drop them. Ordering matches wire arrival. nil/omit
	// when the invocation produced no notices.
	Notices []PostgresV3Notice `json:"notices,omitempty" yaml:"notices,omitempty" bson:"notices,omitempty"`

	// Notifications captures NotificationResponse ('A') messages —
	// async LISTEN/NOTIFY deliveries the server injected into the
	// response stream. These are connection-scoped in PG (a
	// NotificationResponse can arrive between any two bundles), but
	// the capture attaches them to whichever invocation is currently
	// in flight; replay re-emits them before CommandComplete on the
	// same invocation. Notifications that arrive when no invocation
	// is pending (true async between-command delivery) are dropped
	// — supporting those would require a connection-level sideband
	// and a separate injection scheduler the replayer does not yet
	// have. nil/omit when this invocation observed none.
	Notifications []PostgresV3Notification `json:"notifications,omitempty" yaml:"notifications,omitempty" bson:"notifications,omitempty"`

	// FunctionCall captures a legacy fastpath FunctionCall ('F') +
	// FunctionCallResponse ('V') exchange. Mutually exclusive with
	// Rows / CopyOut / CopyIn — an invocation is either a SQL query
	// or a fastpath function invocation, never both. nil/omit on
	// non-FunctionCall invocations.
	FunctionCall *PostgresV3FunctionCall `json:"functionCall,omitempty" yaml:"functionCall,omitempty" bson:"function_call,omitempty"`
}

// PostgresV3Notice is one NoticeResponse ('N') message. Wire format
// mirrors ErrorResponse — same field codes (S/V/C/M/D/H/P/W) but
// severity is always one of NOTICE/WARNING/INFO/DEBUG/LOG rather than
// ERROR/FATAL/PANIC. Only the fields we observe in practice are
// persisted; unused fields stay empty/omit to keep the YAML lean.
type PostgresV3Notice struct {
	Severity string `json:"severity,omitempty" yaml:"severity,omitempty" bson:"severity,omitempty"`
	Code     string `json:"code,omitempty" yaml:"code,omitempty" bson:"code,omitempty"`
	Message  string `json:"message,omitempty" yaml:"message,omitempty" bson:"message,omitempty"`
	Detail   string `json:"detail,omitempty" yaml:"detail,omitempty" bson:"detail,omitempty"`
	Hint     string `json:"hint,omitempty" yaml:"hint,omitempty" bson:"hint,omitempty"`
}

// MarshalYAML routes Notice's free-form text fields through
// PostgresV3SafeString so the on-disk YAML never picks the literal
// block-scalar style for content that yaml.v3's parser then rejects
// inside a sequence (Notices live in PostgresV3Response.Notices,
// which is the offending shape). Field types stay `string` so JSON,
// gob, BSON, and direct Go-to-Go comparisons keep working — the
// alias is YAML-side only.
func (n PostgresV3Notice) MarshalYAML() (any, error) {
	type alias struct {
		Severity PostgresV3SafeString `yaml:"severity,omitempty"`
		Code     PostgresV3SafeString `yaml:"code,omitempty"`
		Message  PostgresV3SafeString `yaml:"message,omitempty"`
		Detail   PostgresV3SafeString `yaml:"detail,omitempty"`
		Hint     PostgresV3SafeString `yaml:"hint,omitempty"`
	}
	return alias{
		Severity: PostgresV3SafeString(n.Severity),
		Code:     PostgresV3SafeString(n.Code),
		Message:  PostgresV3SafeString(n.Message),
		Detail:   PostgresV3SafeString(n.Detail),
		Hint:     PostgresV3SafeString(n.Hint),
	}, nil
}

// PostgresV3Notification is one NotificationResponse ('A') message
// (async LISTEN/NOTIFY delivery). Wire layout: int32 backendPID +
// cstring channel + cstring payload. We persist all three so the
// replay can reproduce what the real backend produced, including
// the PID (some drivers gate on it matching the session's backend
// PID before surfacing the notification to application code).
type PostgresV3Notification struct {
	BackendPID int32  `json:"backendPID,omitempty" yaml:"backendPID,omitempty" bson:"backend_pid,omitempty"`
	Channel    string `json:"channel" yaml:"channel" bson:"channel"`
	Payload    string `json:"payload,omitempty" yaml:"payload,omitempty" bson:"payload,omitempty"`
}

// PostgresV3FunctionCall is the legacy fastpath FunctionCall ('F') →
// FunctionCallResponse ('V') pair. Modern drivers use SELECT fn(...)
// queries and never touch this API, but libpq's PQfn, pgBouncer's
// internal pings, and a few ORM internals (especially around large-
// object access) do emit it — a recorder that treats the tag as
// "unknown" would terminate the stream there.
//
// All three fields are optional in the on-disk YAML:
//   - OID identifies the server-side function (often
//     lo_creat/lo_open/lo_read/lo_write/lo_close for large-object APIs)
//   - Args preserves argument values (logical Go form, see
//     PostgresV3Cell — text or binary wire format per ArgFormats)
//   - Result holds the server's response value (NULL → Cell.Value == nil,
//     i.e. Cell.IsNull() == true; the wire-level int32(-1) sentinel is
//     translated to/from this representation by the codec)
type PostgresV3FunctionCall struct {
	// OID — server-side function OID being invoked.
	OID uint32 `json:"oid" yaml:"oid" bson:"oid"`
	// ArgFormats — per-argument wire format (0 text, 1 binary). Spec
	// allows len=0 (all text), len=1 (broadcast), len=N (per-arg);
	// we preserve exactly what the client sent.
	ArgFormats []int16 `json:"argFormats,omitempty" yaml:"argFormats,omitempty" bson:"arg_formats,omitempty"`
	// Args — ordered list of argument values. A NULL argument is
	// represented by Cell.Value == nil (Cell.IsNull() == true); non-
	// NULL args carry the logical Go value chosen by the codec for
	// the argument's OID (int64, string, []byte, etc.).
	Args []PostgresV3Cell `json:"args,omitempty" yaml:"args,omitempty" bson:"args,omitempty"`
	// ResultFormat — wire format of the result (0 text, 1 binary).
	ResultFormat int16 `json:"resultFormat,omitempty" yaml:"resultFormat,omitempty" bson:"result_format,omitempty"`
	// Result — FunctionCallResponse payload. Cell.Value == nil
	// (Cell.IsNull() == true) represents the wire int32(-1) sentinel
	// (NULL); a non-NULL result carries the codec-chosen logical Go
	// value for the function's return OID.
	Result PostgresV3Cell `json:"result,omitempty" yaml:"result,omitempty" bson:"result,omitempty"`
}

// PostgresV3CopyOutPayload captures a full server-side CopyOut burst:
// the CopyOutResponse header plus the ordered list of CopyData chunks
// the backend produced, terminated by an implicit CopyDone. The raw
// bytes are stored as []byte slices — yaml.v3 base64-encodes byte
// slices automatically, so non-printable bytes and NULs round-trip
// without sentinel games. Each element of Data is exactly one wire
// CopyData ('d') packet's body, preserved in arrival order, because
// clients observe packet boundaries when streaming the output.
type PostgresV3CopyOutPayload struct {
	// OverallFormat mirrors the first byte of CopyOutResponse. 0 = text
	// mode (psql \COPY default), 1 = binary mode. Required so the replay
	// emitter reproduces the header exactly — drivers branch on this.
	OverallFormat byte `json:"overallFormat" yaml:"overallFormat" bson:"overall_format"`
	// ColumnFormatCodes mirrors the per-column format array from
	// CopyOutResponse. len == 0 is valid (COPY of a tableless expression
	// can produce no columns). Each code is 0 = text or 1 = binary. We
	// use uint16 to match the wire encoding exactly.
	ColumnFormatCodes []uint16 `json:"columnFormatCodes,omitempty" yaml:"columnFormatCodes,omitempty" bson:"column_format_codes,omitempty"`
	// Data is the ordered list of CopyData packet bodies the server
	// produced between CopyOutResponse and CopyDone. Each []byte is one
	// packet's payload bytes (NOT concatenated); preserving packet
	// boundaries matches what a real client's wire read loop would see.
	// An empty / nil slice represents a COPY that produced zero rows.
	Data [][]byte `json:"data,omitempty" yaml:"data,omitempty" bson:"data,omitempty"`
}

// PostgresV3CopyInPayload captures only the CopyInResponse header —
// format + per-column format codes. During replay the server side
// emits CopyInResponse, then reads and discards whatever CopyData the
// client produces, then closes the sub-protocol with a CommandComplete
// + ReadyForQuery derived from the invocation's CommandComplete /
// txTransition fields. The client-produced bytes themselves are not
// persisted because they are re-generated on every replay by the
// client under test.
type PostgresV3CopyInPayload struct {
	OverallFormat     byte     `json:"overallFormat" yaml:"overallFormat" bson:"overall_format"`
	ColumnFormatCodes []uint16 `json:"columnFormatCodes,omitempty" yaml:"columnFormatCodes,omitempty" bson:"column_format_codes,omitempty"`
}

type PostgresV3ColumnDescriptor struct {
	Name       string `json:"name" yaml:"name" bson:"name"`
	TableOID   uint32 `json:"tableOid,omitempty" yaml:"tableOid,omitempty" bson:"table_oid,omitempty"`
	ColAttrNum int16  `json:"colAttrNum,omitempty" yaml:"colAttrNum,omitempty" bson:"col_attr_num,omitempty"`
	TypeOID    uint32 `json:"typeOid" yaml:"typeOid" bson:"type_oid"`
	TypeSize   int16  `json:"typeSize,omitempty" yaml:"typeSize,omitempty" bson:"type_size,omitempty"`
	TypeMod    int32  `json:"typeMod,omitempty" yaml:"typeMod,omitempty" bson:"type_mod,omitempty"`
	Format     int16  `json:"format,omitempty" yaml:"format,omitempty" bson:"format,omitempty"`
}

type PostgresV3Error struct {
	Severity string `json:"severity,omitempty" yaml:"severity,omitempty" bson:"severity,omitempty"`
	Code     string `json:"code,omitempty" yaml:"code,omitempty" bson:"code,omitempty"`
	Message  string `json:"message,omitempty" yaml:"message,omitempty" bson:"message,omitempty"`
	Detail   string `json:"detail,omitempty" yaml:"detail,omitempty" bson:"detail,omitempty"`
	Hint     string `json:"hint,omitempty" yaml:"hint,omitempty" bson:"hint,omitempty"`
}

// MarshalYAML — see PostgresV3Notice.MarshalYAML. Same rationale: SQL
// errors carry server-formatted multi-line detail/hint strings that
// can include tabs (e.g. constraint diagnostic listings), so the YAML
// emit needs the safe-string wrapper to stay round-trippable.
func (e PostgresV3Error) MarshalYAML() (any, error) {
	type alias struct {
		Severity PostgresV3SafeString `yaml:"severity,omitempty"`
		Code     PostgresV3SafeString `yaml:"code,omitempty"`
		Message  PostgresV3SafeString `yaml:"message,omitempty"`
		Detail   PostgresV3SafeString `yaml:"detail,omitempty"`
		Hint     PostgresV3SafeString `yaml:"hint,omitempty"`
	}
	return alias{
		Severity: PostgresV3SafeString(e.Severity),
		Code:     PostgresV3SafeString(e.Code),
		Message:  PostgresV3SafeString(e.Message),
		Detail:   PostgresV3SafeString(e.Detail),
		Hint:     PostgresV3SafeString(e.Hint),
	}, nil
}

type PostgresV3SideEffects struct {
	SequenceEmissions []PostgresV3SeqEmission `json:"sequenceEmissions,omitempty" yaml:"sequenceEmissions,omitempty" bson:"sequence_emissions,omitempty"`
	RowMutations      []PostgresV3RowMutation `json:"rowMutations,omitempty" yaml:"rowMutations,omitempty" bson:"row_mutations,omitempty"`
	TxTransition      string                  `json:"txTransition,omitempty" yaml:"txTransition,omitempty" bson:"tx_transition,omitempty"`
}

type PostgresV3SeqEmission struct {
	Sequence string `json:"sequence" yaml:"sequence" bson:"sequence"`
	Value    int64  `json:"value" yaml:"value" bson:"value"`
}

type PostgresV3RowMutation struct {
	Op      string            `json:"op" yaml:"op" bson:"op"`
	Schema  string            `json:"schema" yaml:"schema" bson:"schema"`
	Table   string            `json:"table" yaml:"table" bson:"table"`
	PK      []string          `json:"pk,omitempty" yaml:"pk,omitempty" bson:"pk,omitempty"`
	Columns map[string]string `json:"columns,omitempty" yaml:"columns,omitempty" bson:"columns,omitempty"`
}

// PostgresV3GeneratorSpec — one deterministic volatile-value stream
// (sequence, clock, uuid).
type PostgresV3GeneratorSpec struct {
	Name           string   `json:"name" yaml:"name" bson:"name"`
	Type           string   `json:"type" yaml:"type" bson:"type"`
	RecordedValues []string `json:"recordedValues,omitempty" yaml:"recordedValues,omitempty" bson:"recorded_values,omitempty"`
	Policy         string   `json:"policy,omitempty" yaml:"policy,omitempty" bson:"policy,omitempty"`
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

	c.Spec.MongoRequests = make([]MongoRequest, len(m.Spec.MongoRequests))
	copy(c.Spec.MongoRequests, m.Spec.MongoRequests)

	c.Spec.MongoResponses = make([]MongoResponse, len(m.Spec.MongoResponses))
	copy(c.Spec.MongoResponses, m.Spec.MongoResponses)

	c.Spec.MySQLRequests = make([]mysql.Request, len(m.Spec.MySQLRequests))
	copy(c.Spec.MySQLRequests, m.Spec.MySQLRequests)

	c.Spec.MySQLResponses = make([]mysql.Response, len(m.Spec.MySQLResponses))
	copy(c.Spec.MySQLResponses, m.Spec.MySQLResponses)

	c.Spec.PostgresRequestsV2 = make([]postgres.Request, len(m.Spec.PostgresRequestsV2))
	copy(c.Spec.PostgresRequestsV2, m.Spec.PostgresRequestsV2)

	c.Spec.PostgresResponsesV2 = make([]postgres.Response, len(m.Spec.PostgresResponsesV2))
	copy(c.Spec.PostgresResponsesV2, m.Spec.PostgresResponsesV2)

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

	// PostgresV3 spec: clone the top-level discriminator plus whichever
	// sub-pointer is populated. Each sub-spec is copied by value; that
	// detaches the pointer identity so async gob-write paths and other
	// race-sensitive consumers cannot mutate the original through a
	// shared pointer. Nested slice/map fields (e.g. Query.Response.Rows)
	// are carried by value — they are treated as immutable after ingest
	// on both the record and replay sides, matching how the other
	// *Spec fields above share backing slices.
	if m.Spec.PostgresV3 != nil {
		pgV3Copy := *m.Spec.PostgresV3
		if m.Spec.PostgresV3.Session != nil {
			sessionCopy := *m.Spec.PostgresV3.Session
			pgV3Copy.Session = &sessionCopy
		}
		if m.Spec.PostgresV3.Catalog != nil {
			catalogCopy := *m.Spec.PostgresV3.Catalog
			pgV3Copy.Catalog = &catalogCopy
		}
		if m.Spec.PostgresV3.Data != nil {
			dataCopy := *m.Spec.PostgresV3.Data
			pgV3Copy.Data = &dataCopy
		}
		if m.Spec.PostgresV3.Query != nil {
			queryCopy := *m.Spec.PostgresV3.Query
			pgV3Copy.Query = &queryCopy
		}
		if m.Spec.PostgresV3.Generator != nil {
			generatorCopy := *m.Spec.PostgresV3.Generator
			pgV3Copy.Generator = &generatorCopy
		}
		c.Spec.PostgresV3 = &pgV3Copy
	}

	return &c
}

type ReRecordCfg struct {
	Rerecord bool
	TestSet  string
}
