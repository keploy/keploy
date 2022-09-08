package models

import "gopkg.in/yaml.v3"

type Kind string

const (
	V1_BETA1 Version = Version("api.keploy.io/v1beta1")
)

type Version string

const (
	HTTP_EXPORT    Kind = "Http"
	GENERIC_EXPORT Kind = "Generic"
)

type Mock struct {
	Version  string    `json:"version" yaml:"version"`
	Kind     string    `json:"kind" yaml:"kind"`
	Name     string    `json:"name" yaml:"name"`
	Spec     yaml.Node `json:"spec" yaml:"spec"`
}

type GenericSpec struct {
	Metadata map[string]string `json:"metadata" yaml:"metadata"`
	Objects  []Object          `json:"objects" yaml:"objects"`
}

type Object struct {
	Type string `json:"type" yaml:"type"`
	Data string `json:"data" yaml:"data"`
}

type HttpSpec struct {
	Metadata   map[string]string   `json:"metadata" yaml:"metadata"`
	Request    HttpReq             `json:"req" yaml:"req"`
	Response   HttpResp            `json:"resp" yaml:"resp"`
	Objects    []Object            `json:"objects" yaml:"objects"`
	Mocks      []string            `json:"mocks" yaml:"mocks,omitempty"`
	Assertions map[string][]string `json:"assertions" yaml:"assertions,omitempty"`
	Created   int64              `json:"created" yaml:"created,omitempty"`
}
