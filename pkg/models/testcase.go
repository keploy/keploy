package models

type Kind string
type BodyType string
type Version string

const V1Beta1 = Version("api.keploy.io/v1beta1")

var (
	currentVersion = V1Beta1
)

func SetVersion(V1 string) {
	currentVersion = Version(V1)
}

func GetVersion() (V1 Version) {
	return currentVersion
}

//TODO: Why are we declaring mock types in testcase.go file?

// mocks types
const (
	HTTP           Kind     = "Http"
	GENERIC        Kind     = "Generic"
	REDIS          Kind     = "Redis"
	SQL            Kind     = "SQL"
	Postgres       Kind     = "Postgres"
	GRPC_EXPORT    Kind     = "gRPC"
	Mongo          Kind     = "Mongo"
	BodyTypeUtf8   BodyType = "utf-8"
	BodyTypeBinary BodyType = "binary"
	BodyTypePlain  BodyType = "PLAIN"
	BodyTypeJSON   BodyType = "JSON"
	BodyTypeError  BodyType = "ERROR"
)

type TestCase struct {
	Version  Version             `json:"version" bson:"version"`
	Kind     Kind                `json:"kind" bson:"kind"`
	Name     string              `json:"name" bson:"name"`
	Created  int64               `json:"created" bson:"created"`
	Updated  int64               `json:"updated" bson:"updated"`
	Captured int64               `json:"captured" bson:"captured"`
	HTTPReq  HTTPReq             `json:"http_req" bson:"http_req"`
	HTTPResp HTTPResp            `json:"http_resp" bson:"http_resp"`
	AllKeys  map[string][]string `json:"all_keys" bson:"all_keys"`
	GrpcResp GrpcResp            `json:"grpcResp" bson:"grpcResp"`
	GrpcReq  GrpcReq             `json:"grpcReq" bson:"grpcReq"`
	Anchors  map[string][]string `json:"anchors" bson:"anchors"`
	Noise    map[string][]string `json:"noise" bson:"noise"`
	Mocks    []*Mock             `json:"mocks" bson:"mocks"`
	Type     string              `json:"type" bson:"type"`
	Curl     string              `json:"curl" bson:"curl"`
}

func (tc *TestCase) GetKind() string {
	return string(tc.Kind)
}

type NoiseParams struct {
	TestCaseIDs string              `json:"testCaseIDs"`
	EditedBy    string              `json:"editedBy"`
	Assertion   map[string][]string `json:"assertion"`
	Ops         string              `json:"ops"`
}

// enum for ops
const (
	OpsAdd    = "ADD"
	OpsRemove = "REMOVE"
)