package models

type BodyType string
type Version string

const V1Beta1 = Version("api.keploy.io/v1beta1")

// BodyType constants for HTTP and gRPC
const (
	JSON            BodyType = "JSON"
	XML             BodyType = "XML"
	Text            BodyType = "TEXT"
	Plain           BodyType = "PLAIN"
	Utf8            BodyType = "utf-8"
	Binary          BodyType = "BINARY"
	GrpcCompression BodyType = "GRPC_COMPRESSION"
	GrpcLength      BodyType = "GRPC_LENGTH"
	GrpcData        BodyType = "GRPC_DATA"
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

type TestCase struct {
	Version     Version                       `json:"version" bson:"version"`
	Kind        Kind                          `json:"kind" bson:"kind"`
	Name        string                        `json:"name" bson:"name"`
	Description string                        `json:"description" bson:"description"`
	Created     int64                         `json:"created" bson:"created"`
	Updated     int64                         `json:"updated" bson:"updated"`
	Captured    int64                         `json:"captured" bson:"captured"`
	HTTPReq     HTTPReq                       `json:"http_req" bson:"http_req"`
	HTTPResp    HTTPResp                      `json:"http_resp" bson:"http_resp"`
	AllKeys     map[string][]string           `json:"all_keys" bson:"all_keys"`
	GrpcResp    GrpcResp                      `json:"grpcResp" bson:"grpcResp"`
	GrpcReq     GrpcReq                       `json:"grpcReq" bson:"grpcReq"`
	Anchors     map[string][]string           `json:"anchors" bson:"anchors"`
	Noise       map[string][]string           `json:"noise" bson:"noise"`
	Mocks       []*Mock                       `json:"mocks" bson:"mocks"`
	Type        string                        `json:"type" bson:"type"`
	Curl        string                        `json:"curl" bson:"curl"`
	IsLast      bool                          `json:"is_last" bson:"is_last"`
	Assertions  map[AssertionType]interface{} `json:"assertion" bson:"assertion"`
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
