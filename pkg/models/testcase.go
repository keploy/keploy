package models

type Kind string
type BodyType string
type Version string

const (
	V1Beta1 Version = Version("api.keploy.io/v1beta1")
	V1Beta2 Version = Version("api.keploy.io/v1beta2")
)

const (
	HTTP           Kind     = "Http"
	GENERIC        Kind     = "Generic"
	SQL            Kind     = "SQL"
	GRPC_EXPORT    Kind     = "gRPC"
	Postgres	   Kind     = "Postgres"
	Mongo          Kind     = "Mongo"
	BodyTypeUtf8   BodyType = "utf-8"
	BodyTypeBinary BodyType = "binary"
	BodyTypePlain  BodyType = "PLAIN"
	BodyTypeJSON   BodyType = "JSON"
	BodyTypeError  BodyType = "ERROR"
)

type TestCase struct {
	Version Version   `json:"version"`
	Kind    Kind      `json:"kind"`
	Name    string    `json:"name"`
	Created  int64  `json:"created"`
	Updated  int64  `json:"updated"`
	Captured int64  `json:"captured"`
	HttpReq  HttpReq             `json:"http_req"`
	HttpResp HttpResp            `json:"http_resp"`
	AllKeys  map[string][]string `json:"all_keys"`
	Anchors  map[string][]string `json:"anchors"`
	Noise    []string            `json:"noise"`
	Mocks    []*Mock       `json:"mocks"`
	Type     string              `json:"type"`
}
