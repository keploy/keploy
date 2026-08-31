package models

import "time"

type BodyType string
type Version string

const V1Beta1 = Version("api.keploy.io/v1beta1")

// BodyType constants for HTTP and gRPC
const (
	JSON            BodyType = "JSON"
	XML             BodyType = "XML"
	HTML            BodyType = "HTML"
	CSV             BodyType = "CSV"
	Plain           BodyType = "PLAIN"
	Utf8            BodyType = "utf-8"
	Binary          BodyType = "BINARY"
	GrpcCompression BodyType = "GRPC_COMPRESSION"
	GrpcLength      BodyType = "GRPC_LENGTH"
	GrpcData        BodyType = "GRPC_DATA"
	UnknownType     BodyType = "UNKNOWN"
)

var (
	currentVersion = V1Beta1
)

func SetVersion(V1 string) {
	currentVersion = Version(V1)
}

func GetVersion() (V1 Version) {
	return currentVersion
}

type LastUpdated struct {
	Author    string    `json:"author,omitempty" bson:"author,omitempty" yaml:"author,omitempty"`
	Timestamp time.Time `json:"timestamp,omitempty" bson:"timestamp,omitempty" yaml:"timestamp,omitempty"`
}

type TestCase struct {
	Version       Version                       `json:"version" bson:"version"`
	Kind          Kind                          `json:"kind" bson:"kind"`
	Name          string                        `json:"name" bson:"name"`
	Description   string                        `json:"description" bson:"description"`
	Created       int64                         `json:"created" bson:"created"`
	Updated       int64                         `json:"updated" bson:"updated"`
	Captured      int64                         `json:"captured" bson:"captured"`
	LastUpdated   *LastUpdated                  `json:"last_updated,omitempty" bson:"last_updated,omitempty" yaml:"last_updated,omitempty"`
	HTTPReq       HTTPReq                       `json:"http_req" bson:"http_req"`
	HTTPResp      HTTPResp                      `json:"http_resp" bson:"http_resp"`
	AllKeys       map[string][]string           `json:"all_keys" bson:"all_keys"`
	GrpcResp      GrpcResp                      `json:"grpcResp" bson:"grpcResp"`
	GrpcReq       GrpcReq                       `json:"grpcReq" bson:"grpcReq"`
	Anchors       map[string][]string           `json:"anchors" bson:"anchors"`
	Noise         map[string][]string           `json:"noise" bson:"noise"`
	Mocks         []*Mock                       `json:"mocks" bson:"mocks"`
	Type          string                        `json:"type" bson:"type"`
	Curl          string                        `json:"curl" bson:"curl"`
	IsLast        bool                          `json:"is_last" bson:"is_last"`
	Assertions    map[AssertionType]interface{} `json:"assertion" bson:"assertion"`
	HasBinaryFile bool                          `json:"has_binary_file" bson:"has_binary_file"`
	AppPort       uint16                        `json:"app_port" bson:"app_port"`
	// SourcePod is a transient, never-persisted routing tag: the name of the
	// pod whose traffic produced this test case. Reentrancy seam for the
	// enterprise DaemonSet agent's per-pod attribution — it stamps this from
	// the capture context so the uploader can carry a per-pod source to the
	// control plane. json/yaml/bson "-" so it never lands in stored test-case
	// files or the upload body.
	SourcePod string `json:"-" yaml:"-" bson:"-"`
	// SchemaKey is the canonical static-dedup fingerprint of this test case,
	// stamped by the enterprise capture hook when cross-pod dedup is on. It
	// rides only the agent→control-plane upload (JSON) so the recording owner
	// can detect duplicates across pods; yaml/bson "-" keeps it out of stored
	// test-case files and backend rows. Empty when static dedup is off or the
	// fingerprint could not be computed — consumers must treat empty as
	// "cannot judge", never as "unique".
	SchemaKey string `json:"schema_key,omitempty" yaml:"-" bson:"-"`
	// SourceNode is a transient, never-persisted dedup-scope tag mirroring
	// SourcePod: the node whose (node-scoped) DaemonSet deduper first counted
	// this capture. The k8s-proxy control plane stamps it from the capture
	// frame at ingest so its cross-pod dedup funnel can tell "this scope's
	// own first copy" from "another scope's duplicate" — the scope must ride
	// each capture, not be inferred from mutable registries. Never set by
	// agents or on the wire ("-" everywhere).
	SourceNode string `json:"-" yaml:"-" bson:"-"`
	// DuplicateOf marks this test case as a cross-pod duplicate within its
	// recording: "<test-set>/<test-name>" of the first-stored copy, or a
	// scope-only ref ("pod:<p>" / "node:<n>") when the winner's identity was
	// recovered from dedup-stat snapshots rather than a stored test case. Set
	// by the recording owner, never by agents. Persisted — JSON/BSON here,
	// YAML via the spec Metadata map — so the mark survives to storage and
	// the UI. Empty means not a known duplicate.
	DuplicateOf string `json:"duplicate_of,omitempty" bson:"duplicate_of,omitempty"`
}

func (tc *TestCase) GetKind() string {
	return string(tc.Kind)
}

type NoiseParams struct {
	TestCaseID string              `json:"testCaseID"`
	EditedBy   string              `json:"editedBy"`
	Assertion  map[string][]string `json:"assertion"`
	Ops        string              `json:"ops"`
	AfterNoise map[string][]string `json:"afterNoise"`
}

// enum for ops
const (
	OpsAdd    = "ADD"
	OpsRemove = "REMOVE"
)
