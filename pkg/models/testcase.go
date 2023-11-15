package models

import "go.mongodb.org/mongo-driver/bson/primitive"

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
	Version  Version             `json:"version" bson:"version"`
	Kind     Kind                `json:"kind" bson:"kind"`
	Name     string              `json:"name" bson:"name"`
	Created  int64               `json:"created" bson:"created"`
	Updated  int64               `json:"updated" bson:"updated"`
	Captured int64               `json:"captured" bson:"captured"`
	HttpReq  HttpReq             `json:"http_req" bson:"http_req"`
	HttpResp HttpResp            `json:"http_resp" bson:"http_resp"`
	AllKeys  map[string][]string `json:"all_keys" bson:"all_keys"`
	GrpcResp GrpcResp            `json:"grpcResp" bson:"grpcResp"`
	GrpcReq  GrpcReq             `json:"grpcReq" bson:"grpcReq"`
	Anchors  map[string][]string `json:"anchors" bson:"anchors"`
	Noise    map[string][]string `json:"noise" bson:"noise"`
	Mocks    []*Mock             `json:"mocks" bson:"mocks"`
	Type     string              `json:"type" bson:"type"`
	ID       *primitive.ObjectID `bson:"_id,omitempty"`
}
