package models

import (
	"context"

	"gopkg.in/yaml.v3"
)

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
	Version string    `json:"version" yaml:"version"`
	Kind    string    `json:"kind" yaml:"kind"`
	Name    string    `json:"name" yaml:"name"`
	Spec    yaml.Node `json:"spec" yaml:"spec"`
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
	Request    MockHttpReq         `json:"req" yaml:"req"`
	Response   MockHttpResp        `json:"resp" yaml:"resp"`
	Objects    []Object            `json:"objects" yaml:"objects"`
	Mocks      []string            `json:"mocks" yaml:"mocks,omitempty"`
	Assertions map[string][]string `json:"assertions" yaml:"assertions,omitempty"`
	Created    int64               `json:"created" yaml:"created,omitempty"`
}

type MockHttpReq struct {
	Method     Method            `json:"method" yaml:"method"`
	ProtoMajor int               `json:"proto_major" yaml:"proto_major"` // e.g. 1
	ProtoMinor int               `json:"proto_minor" yaml:"proto_minor"` // e.g. 0
	URL        string            `json:"url" yaml:"url"`
	URLParams  map[string]string `json:"url_params" yaml:"url_params,omitempty"`
	Header     map[string]string `json:"header" yaml:"header"`
	Body       string            `json:"body" yaml:"body"`
}

type MockHttpResp struct {
	StatusCode int               `json:"status_code" yaml:"status_code"` // e.g. 200
	Header     map[string]string `json:"header" yaml:"header"`
	Body       string            `json:"body" yaml:"body"`
}

type MockStore interface {
	ReadAll(ctx context.Context, testCasePath, mockPath string) ([]TestCase, error)
	Read(path, name string, libMode bool) ([]Mock, error)
	Write(ctx context.Context, path string, doc Mock) error
	WriteAll(ctx context.Context, path, fileName string, docs []Mock) error
}
