package models

type Mock struct {
	Version string     `json:"version" yaml:"version"`
	Kind    string     `json:"kind" yaml:"kind"`
	Name    string     `json:"name" yaml:"name"`
	Spec    SpecSchema `json:"spec" yaml:"spec"`
}

type SpecSchema struct {
	Type       string              `json:"type" yaml:"type"`
	Metadata   map[string]string   `json:"metadata" yaml:"metadata"`
	Request    HttpReq             `json:"req" yaml:"req,omitempty"`
	Response   HttpResp            `json:"resp" yaml:"resp,omitempty"`
	Objects    []Object            `json:"objects" yaml:"objects"`
	Mocks      []string            `json:"mocks" yaml:"mocks,omitempty"`
	Assertions map[string][]string `json:"assertions" yaml:"assertions,omitempty"`
}

type Object struct {
	Type string `json:"type" yaml:"type"`
	Data string `json:"data" yaml:"data"`
}
