package models

type Kind string
type BodyType string
type Version string

const V1Beta1 = Version("api.keploy.io/v1beta1")

var (
	CurrentVersion = V1Beta1
)

func SetVersion(V1 string){
	CurrentVersion = Version(V1)
}

func GetVersion() (V1 Version){
	return V1Beta1
}

const (
	HTTP           Kind     = "Http"
	GENERIC        Kind     = "Generic"
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
	Version  Version             `json:"version"`
	Kind     Kind                `json:"kind"`
	Name     string              `json:"name"`
	Created  int64               `json:"created"`
	Updated  int64               `json:"updated"`
	Captured int64               `json:"captured"`
	HttpReq  HttpReq             `json:"http_req"`
	HttpResp HttpResp            `json:"http_resp"`
	AllKeys  map[string][]string `json:"all_keys"`
	GrpcResp GrpcResp            `json:"grpcResp"`
	GrpcReq  GrpcReq             `json:"grpcReq"`
	Anchors  map[string][]string `json:"anchors"`
	Noise    []string            `json:"noise"`
	Mocks    []*Mock             `json:"mocks"`
	Type     string              `json:"type"`
}
