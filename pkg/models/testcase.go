package models

type Kind string
type BodyType string
type Version string

var (
	V1Beta1 Version
	V1Beta2 Version
)

func SetVersion(V1 string, V2 string){
	V1Beta1 = Version(V1)
	V1Beta2 = Version(V2)
}

func GetVersion() (V1 Version, V2 Version){
	return V1Beta1, V1Beta2
}

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
