package models

import (
	"gopkg.in/yaml.v3"
)

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
	BodyTypeUtf8   BodyType = "utf-8"
	BodyTypeBinary BodyType = "binary"
	BodyTypePlain  BodyType = "PLAIN"
	BodyTypeJSON   BodyType = "JSON"
	BodyTypeError  BodyType = "ERROR"
)

type Mock struct {
	Version Version   `json:"version" yaml:"version"`
	Kind    Kind      `json:"kind" yaml:"kind"`
	Name    string    `json:"name" yaml:"name"`
	Spec    yaml.Node `json:"spec" yaml:"spec"`
}
