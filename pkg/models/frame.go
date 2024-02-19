package models

type Frame struct {
	Version Version     `json:"version" yaml:"version"`
	Kind    Kind        `json:"kind" yaml:"kind"`
	Name    string      `json:"name" yaml:"name"`
	Spec    interface{} `json:"spec" yaml:"spec"`
	Curl    string      `json:"curl" yaml:"curl,omitempty"`
}

type Spec struct {
}
